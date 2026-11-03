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

// Delivery is supplied by the existing write boundary. History returns the keys
// of trusted hive attention markers (evidence of a real POST, not status-card
// condition markers), with pagination. Find must reconcile this exact attempt,
// not content or another hive's attempt.
type Delivery interface {
	History(context.Context, string, int) ([]string, error)
	Find(context.Context, Episode) (bool, error)
	Post(context.Context, Episode, string) error
}

type disk struct {
	Version int             `json:"version"`
	Items   map[string]bool `json:"items"`
	// ItemSeen is additive: ledgers written before it existed load with zero times.
	ItemSeen map[string]time.Time `json:"item_seen,omitempty"`
	Episodes map[string]*Episode  `json:"episodes"`
}

// gate serializes Reports for one item while s.mu is released.
type gate struct {
	mu   sync.Mutex
	refs int
}

type Store struct {
	mu    sync.Mutex
	gates map[string]*gate
	path  string
	state disk
	now   func() time.Time
}

// Open treats missing/corrupt state as recovery, never as permission to replay.
// Unknown versions are refused rather than clobbered during rolling upgrades.
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now, state: disk{Version: 1, Items: map[string]bool{}, ItemSeen: map[string]time.Time{}, Episodes: map[string]*Episode{}}}
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
	if d.ItemSeen == nil {
		d.ItemSeen = map[string]time.Time{}
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

// prune drops resolved episodes once recurrence would start a fresh episode
// anyway, so the capacity cap bounds live inventory rather than eventually
// refusing every report. Uncertain episodes stay until reconciled, items drop once they have no episodes, and
// history-seeded hash-only baselines (no Key, never observed) are dropped once
// the window has passed, when a recurrence would start a fresh episode anyway.
func (s *Store) prune(now time.Time, window time.Duration) {
	for k, e := range s.state.Episodes {
		if e.State == Resolved && !e.Uncertain && !e.ResolvedAt.IsZero() && now.Sub(e.ResolvedAt) > window {
			delete(s.state.Episodes, k)
		} else if e.Baseline && e.State == Observed && e.Key == (Key{}) && now.Sub(e.FirstSeen) > window {
			delete(s.state.Episodes, k)
		}
	}
	live := map[string]bool{}
	for _, e := range s.state.Episodes {
		live[fmt.Sprintf("%s#%d", canonical(e.Key.Repo), e.Key.Item)] = true
	}
	// Re-seeding a dropped item through History on its next report is replay-safe.
	for k := range s.state.Items {
		if !live[k] {
			delete(s.state.Items, k)
			delete(s.state.ItemSeen, k)
		}
	}
}

// Report accepts the full current condition snapshot for one item. The first
// snapshot per item consults delivery history: a condition with a prior trusted
// attention marker is seeded as a baseline WITHOUT sending, while a condition
// that was never paged notifies immediately, ledger loss or not.
// Calls for the same item are serialized through the POST outcome, including
// concurrent producers; s.mu is never held across a Delivery call, so slow
// GitHub listings for one item do not block other items or Snapshot.
// Non-admitters only record would-notify. No marker lookup grants ownership.
func (s *Store) Report(ctx context.Context, cfg Config, hiveID, repo string, item int, observations []Observation, d Delivery) ([]Episode, error) {
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
	release := s.lockItem(itemKey)
	defer release()

	s.mu.Lock()
	now := s.now().UTC()
	s.prune(now, cfg.Window())
	first := !s.state.Items[itemKey]
	var capErr error
	if first {
		capErr = s.admitItem()
	}
	s.mu.Unlock()
	if capErr != nil {
		return nil, capErr
	}
	var history []string
	if first {
		var err error
		if history, err = d.History(ctx, repo, item); err != nil {
			return nil, err
		}
	}
	work, err := s.record(cfg, itemKey, first, history, observations, now)
	if err != nil {
		return nil, err
	}
	for _, o := range work {
		if err := s.deliver(ctx, d, o, hiveID, owner, now); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(repo, item), nil
}

// lockItem serializes Reports per item without holding s.mu.
func (s *Store) lockItem(key string) func() {
	s.mu.Lock()
	if s.gates == nil {
		s.gates = map[string]*gate{}
	}
	g := s.gates[key]
	if g == nil {
		g = &gate{}
		s.gates[key] = g
	}
	g.refs++
	s.mu.Unlock()
	g.mu.Lock()
	return func() {
		g.mu.Unlock()
		s.mu.Lock()
		if g.refs--; g.refs == 0 {
			delete(s.gates, key)
		}
		s.mu.Unlock()
	}
}

// admitItem makes room for one new item. Only items whose episodes are all
// resolved and reconciled are evicted, oldest-touched first; History re-seeds
// them replay-safely. With only open items left it fails closed as before.
// Caller holds s.mu.
func (s *Store) admitItem() error {
	if len(s.state.Items) < MaxItems {
		return nil
	}
	open := map[string]bool{}
	for _, e := range s.state.Episodes {
		if e.State != Resolved || e.Uncertain {
			open[fmt.Sprintf("%s#%d", canonical(e.Key.Repo), e.Key.Item)] = true
		}
	}
	victim := ""
	for k := range s.state.Items {
		if open[k] || s.gates[k] != nil {
			continue
		}
		if victim == "" || s.state.ItemSeen[k].Before(s.state.ItemSeen[victim]) || (s.state.ItemSeen[k].Equal(s.state.ItemSeen[victim]) && k < victim) {
			victim = k
		}
	}
	if victim == "" {
		return errors.New("attention: item capacity reached; refusing replay-prone eviction")
	}
	for h, e := range s.state.Episodes {
		if fmt.Sprintf("%s#%d", canonical(e.Key.Repo), e.Key.Item) == victim {
			delete(s.state.Episodes, h)
		}
	}
	delete(s.state.Items, victim)
	delete(s.state.ItemSeen, victim)
	return nil
}

// record applies the observations under s.mu and persists them, returning the
// observations that still need delivery.
func (s *Store) record(cfg Config, itemKey string, first bool, history []string, observations []Observation, now time.Time) ([]Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	historical := map[string]bool{}
	if first {
		// Another item may have filled the ledger while History was in flight.
		if !s.state.Items[itemKey] {
			if err := s.admitItem(); err != nil {
				return nil, err
			}
		}
		for _, k := range history {
			historical[k] = true
			if s.state.Episodes[k] == nil {
				if len(s.state.Episodes) >= MaxEpisodes {
					return nil, errors.New("attention: recovery capacity reached")
				}
				s.state.Episodes[k] = &Episode{State: Observed, FirstSeen: now, Baseline: true}
			}
		}
	}
	for _, o := range observations {
		hash := o.Key.Hash()
		e := s.state.Episodes[hash]
		if e == nil {
			if len(s.state.Episodes) >= MaxEpisodes {
				return nil, errors.New("attention: episode capacity reached; refusing replay-prone eviction")
			}
			e = &Episode{Key: o.Key, State: Observed, FirstSeen: now, Baseline: historical[hash]}
			s.state.Episodes[hash] = e
		} else if e.State == Resolved && !o.Resolved {
			if now.Sub(e.ResolvedAt) > cfg.Window() && !e.Uncertain {
				e = &Episode{Key: o.Key, State: Observed, FirstSeen: now}
				s.state.Episodes[hash] = e
			} else {
				e.State = Observed
				if !e.PagedAt.IsZero() {
					e.QuietReopen = true
					e.State = Notified
				}
				if !e.AckedAt.IsZero() {
					e.QuietReopen = true
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
	s.state.ItemSeen[itemKey] = now
	if err := s.save(); err != nil {
		return nil, err
	}
	return observations, nil
}

// deliver runs the Find/Post step for one observation. State is read and
// written under s.mu; the Delivery calls themselves run without it.
func (s *Store) deliver(ctx context.Context, d Delivery, o Observation, hiveID, owner string, now time.Time) error {
	s.mu.Lock()
	e := s.state.Episodes[o.Key.Hash()]
	if e == nil {
		s.mu.Unlock()
		return nil
	}
	reconcileOnly := o.Resolved && e.Uncertain && e.Attempt != "" && owner == hiveID && hiveID != ""
	if !reconcileOnly && (!e.WouldNotify || o.Resolved || e.Baseline || owner != hiveID || hiveID == "") {
		s.mu.Unlock()
		return nil
	}
	if !reconcileOnly && e.Attempt != "" && e.AttemptOwner != hiveID {
		// Configuration handoff must not retry a previous admitter's uncertain POST.
		s.mu.Unlock()
		return nil
	}
	needFind := reconcileOnly || e.Attempt != ""
	if !needFind {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			s.mu.Unlock()
			return err
		}
		e.Attempt = hex.EncodeToString(id[:])
		e.AttemptOwner = hiveID
	}
	snap := *e
	s.mu.Unlock()

	if needFind {
		found, err := d.Find(ctx, snap)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if found {
			e.PagedAt = now
			e.PagedTo = e.Key.Recipient
			if !reconcileOnly {
				e.State = Notified
			}
		}
		if found || reconcileOnly {
			e.Uncertain = false
			e.WouldNotify = false
			err := s.save()
			s.mu.Unlock()
			return err
		}
		// Not found and not reconcile-only: fall through to retry the POST
		// with s.mu still held from above.
	} else {
		s.mu.Lock()
	}
	e.Uncertain = true
	err := s.save()
	snap = *e
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := d.Post(ctx, snap, o.Body+"\n\n"+snap.Marker()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.PagedAt = now
	e.PagedTo = e.Key.Recipient
	e.State = Notified
	e.Uncertain = false
	e.WouldNotify = false
	return s.save()
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
